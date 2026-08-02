'use strict';
module.exports = {

  async up(queryInterface, Sequelize)
  {
    await queryInterface.createTable('Connection', {
      
      id: {
        allowNull: false,
        autoIncrement: true,
        primaryKey: true,
        type: Sequelize.INTEGER
      },
      
      dt: {
        type: Sequelize.DATE
      },
      
      uuid: {
        type: Sequelize.STRING
      },
      
      encoding: {
        type: Sequelize.STRING
      },
      
      hello_name: {
        type: Sequelize.STRING
      },

      remoteAddr: {
        type: Sequelize.STRING
      },

      remotePort: {
        type: Sequelize.INTEGER
      },

      remote_host: {
        type: Sequelize.STRING
      },

      remote_info: {
        type: Sequelize.STRING
      },

      remote_is_local: {
        type: Sequelize.INTEGER
      },

      remote_is_private: {
        type: Sequelize.INTEGER
      },

      using_tls: {
        type: Sequelize.INTEGER
      },

      tran_count: {
        type: Sequelize.INTEGER
      },

      rcpt_count_accept: {
        type: Sequelize.INTEGER
      },

      rcpt_count_tempfail: {
        type: Sequelize.INTEGER
      },

      rcpt_count_reject: {
        type: Sequelize.INTEGER
      },

      createdAt: {
        allowNull: false,
        type: Sequelize.DATE
      },

      updatedAt: {
        allowNull: false,
        type: Sequelize.DATE
      }

    });
  },

  async down(queryInterface, Sequelize)
  {
    await queryInterface.dropTable('Connection');
  }
};
