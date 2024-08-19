import React, { useEffect, useState } from 'react';
import { API, showError } from '../../helpers';
import { marked } from 'marked';
import { Layout } from '@douyinfe/semi-ui';

const About = () => {
  const [about, setAbout] = useState('');
  const [aboutLoaded, setAboutLoaded] = useState(false);

  const displayAbout = async () => {
    setAbout(localStorage.getItem('about') || '');
    const res = await API.get('/api/about');
    const { success, message, data } = res.data;
    if (success) {
      let aboutContent = data;
      if (!data.startsWith('https://')) {
        aboutContent = marked.parse(data);
      }
      setAbout(aboutContent);
      localStorage.setItem('about', aboutContent);
    } else {
      showError(message);
      setAbout('加载关于内容失败...');
    }
    setAboutLoaded(true);
  };

  useEffect(() => {
    displayAbout().then();
  }, []);

  return (
    <>
      {aboutLoaded && about === '' ? (
        <>
          <Layout>
            <Layout.Header>
              <h3>关于</h3>
            </Layout.Header>
            <Layout.Content>
              <p>New API 接口聚合管理平台，仅作为内部使用。</p>
              友情链接：   
              <br /><a href='https://chat.aabao.vip'>Fast GPT</a>：三分钟搭建 AI 知识库，专属自己的知识库问答系统
              <br /><a href='https://web.aabao.vip'>ChatGPT Web</a>： 公益 ChatGPT 网页服务，支持 GPT4、GPTs、Mj绘画等多种AI模型
              <p>
                NewAPI © 2024 | 基于 One API v0.5.4 © 2024
              </p>
              <p>
                如有任何问题，请联系管理员微信号：【aabao325】
              </p>
            </Layout.Content>
          </Layout>
        </>
      ) : (
        <>
          {about.startsWith('https://') ? (
            <iframe
              src={about}
              style={{ width: '100%', height: '100vh', border: 'none' }}
            />
          ) : (
            <div
              style={{ fontSize: 'larger' }}
              dangerouslySetInnerHTML={{ __html: about }}
            ></div>
          )}
        </>
      )}
    </>
  );
};

export default About;
